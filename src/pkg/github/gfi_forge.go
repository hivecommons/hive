package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/gfi"
)

// HardSuppressIssueLabels returns a copy of the escalation labels that park
// an issue on a person. Exposed for the good-first-issue scorer, which must
// never advertise a parked issue (and whose own labels are deliberately NOT
// in this set — `good first issue` is an invitation, not a hold).
func HardSuppressIssueLabels() []string {
	return append([]string(nil), hardSuppressIssueLabels...)
}

// UpsertIssueMarkerComment implements gfi.Forge: it finds the first comment
// on repo#number containing marker, edits it to body when body is non-empty
// and differs, and — only when create is set — posts body when none exists.
// With create false and an empty body it is a pure lookup.
func (c *Client) UpsertIssueMarkerComment(ctx context.Context, repo string, number int, marker, body string, create bool) (gfi.MarkerComment, error) {
	if c == nil {
		return gfi.MarkerComment{}, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, repoName, number)
	if err != nil {
		return gfi.MarkerComment{}, err
	}
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), marker) {
			continue
		}
		res := gfi.MarkerComment{Found: true, Previous: cm.GetBody()}
		if body == "" || cm.GetBody() == body {
			return res, nil
		}
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repoName, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return res, fmt.Errorf("editing marker comment on %s/%s#%d: %w", owner, repoName, number, err)
		}
		return res, nil
	}
	if !create || body == "" {
		return gfi.MarkerComment{}, nil
	}
	if err := c.CreateIssueComment(ctx, owner+"/"+repoName, number, body); err != nil {
		return gfi.MarkerComment{}, err
	}
	return gfi.MarkerComment{Created: true}, nil
}

// IssueLabels implements gfi.LabelReader: the issue's labels as GitHub has
// them now, not as the last enumeration recorded them.
func (c *Client) IssueLabels(ctx context.Context, repo string, number int) ([]string, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	return c.StageLabeler().Labels(ctx, owner, repoName, number)
}

var (
	_ gfi.Forge       = (*Client)(nil)
	_ gfi.LabelReader = (*Client)(nil)
)
