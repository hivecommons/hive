package github

import (
	"context"
	"fmt"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// LabelledPR is a PR found by one of its labels, with the updated_at a
// label write on it is journaled against (see addLabelsObserved).
type LabelledPR struct {
	Repo      string
	Number    int
	UpdatedAt time.Time
}

// ClosedPRsWithLabel lists closed or merged PRs under owner that still carry
// label: one Search call, at most limit results, least recently updated
// first. The review verdict labels mirror the state of open PRs only, so
// this is how a label left on a PR that merged or closed is found
// (hivecommons/hive#14361).
func (c *Client) ClosedPRsWithLabel(ctx context.Context, owner, label string, limit int) ([]LabelledPR, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	if owner == "" || label == "" || limit <= 0 {
		return nil, nil
	}
	q := fmt.Sprintf("org:%s is:pr is:closed label:%q", owner, label)
	found, _, err := c.client.Search.Issues(ctx, q, &gh.SearchOptions{Sort: "updated", Order: "asc", ListOptions: gh.ListOptions{PerPage: limit}})
	if err != nil {
		return nil, fmt.Errorf("searching closed PRs labelled %q under %s: %w", label, owner, err)
	}
	var out []LabelledPR
	for _, issue := range found.Issues {
		if !issue.IsPullRequest() || issue.GetNumber() <= 0 {
			continue
		}
		o, r := blockerRepoFromURLs(issue.GetRepositoryURL(), issue.GetHTMLURL())
		if o == "" || r == "" {
			continue
		}
		out = append(out, LabelledPR{Repo: o + "/" + r, Number: issue.GetNumber(), UpdatedAt: issue.GetUpdatedAt().Time})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// RemoveLabelAsOf is RemoveLabel journaled against the item state the caller
// read (its updated_at). The plain RemoveLabel journals "remove X on #N" as
// applied forever, so a label a writer put back after one removal could
// never come off again (hivecommons/hive#12512); a review verdict label is
// put back every time the PR's head is reviewed again.
func (c *Client) RemoveLabelAsOf(ctx context.Context, repo string, number int, label string, updatedAt time.Time) error {
	return c.removeLabelObserved(ctx, repo, number, label, observedState(updatedAt))
}
