package github

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// ReviewPRChangedFiles lists a PR's changed paths for the review planner's
// bot-author model route (review.bot_author_full_review_paths). known is
// false on any failure, including an incomplete list: a PR whose files cannot
// be checked against the full-review paths is not downgraded.
func (c *Client) ReviewPRChangedFiles(ctx context.Context, repo string, number int) (paths []string, known bool) {
	if c == nil || c.client == nil || number <= 0 {
		return nil, false
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	files, err := ListPRChangedFiles(WithRESTCaller(ctx, "hive:review_bot_route"), c.client, owner, name, &gh.PullRequest{Number: gh.Ptr(number)})
	if err != nil {
		return nil, false
	}
	paths = make([]string, 0, len(files))
	for _, f := range files {
		if p := strings.TrimSpace(f.Filename); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, true
}
