package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/firsttimer"
)

// firstTimerForge adapts *Client to firsttimer.Forge. The marker upsert is
// the same one gfi uses; only the result type differs, and pkg/firsttimer
// stays stdlib-only rather than importing pkg/gfi for a three-field struct.
type firstTimerForge struct{ c *Client }

// FirstTimerForge returns the firsttimer.Forge view of the client. A nil
// client yields a forge whose every call returns ErrNoGitHubClient.
func (c *Client) FirstTimerForge() firsttimer.Forge { return firstTimerForge{c: c} }

func (f firstTimerForge) UpsertIssueMarkerComment(ctx context.Context, repo string, number int, marker, body string, create bool) (firsttimer.MarkerComment, error) {
	res, err := f.c.UpsertIssueMarkerComment(ctx, repo, number, marker, body, create)
	return firsttimer.MarkerComment{Found: res.Found, Previous: res.Previous, Created: res.Created}, err
}

// ListAuthorPullRequests implements firsttimer.Forge with one search call:
// the numbers of author's pull requests in repo, any state, oldest first.
func (f firstTimerForge) ListAuthorPullRequests(ctx context.Context, repo, author string, limit int) ([]int, error) {
	return f.c.ListAuthorPullRequests(ctx, repo, author, limit)
}

// ListAuthorPullRequests returns the numbers of pull requests author opened
// in repo (owner/name), any state, oldest first, at most limit (≤ 100).
func (c *Client) ListAuthorPullRequests(ctx context.Context, repo, author string, limit int) ([]int, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	author = strings.TrimSpace(strings.TrimPrefix(author, "@"))
	if author == "" {
		return nil, fmt.Errorf("listing author pull requests: empty author")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	owner, name := c.splitRepo(repo)
	query := fmt.Sprintf("repo:%s/%s is:pr author:%s", owner, name, author)
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{
		Sort: "created", Order: "asc", ListOptions: gh.ListOptions{PerPage: limit},
	})
	if err != nil {
		return nil, fmt.Errorf("searching %s's pull requests in %s/%s: %w", author, owner, name, err)
	}
	out := make([]int, 0, len(result.Issues))
	for _, is := range result.Issues {
		if is != nil && is.GetNumber() > 0 {
			out = append(out, is.GetNumber())
		}
	}
	return out, nil
}

var _ firsttimer.Forge = firstTimerForge{}
