package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// maxPRCommitPages bounds ListPRCommits pagination. GitHub's PR commit list
// endpoint caps at 250 commits per PR; at 100 per page, three pages covers
// the full API ceiling without an unbounded loop.
const maxPRCommitPages = 3

// PRCommit is one commit on a PR's branch, projected down to what the hold
// guard's snapshot and drift comment need.
type PRCommit struct {
	SHA string
	// Author is the GitHub login when the commit maps to an account, falling
	// back to the raw git author name (a commit authored with an unmapped
	// email has no login, and an empty author would hide exactly the foreign
	// commits the guard exists to surface).
	Author string
	// Title is the first line of the commit message.
	Title string
	// Message is the full commit message, including trailers. Empty only when
	// GitHub omitted the embedded commit object.
	Message string
}

// ListPRCommits returns the commits currently on a PR's branch, oldest first
// (GitHub's list order). Nil-client safe like the other read paths.
func (c *Client) ListPRCommits(ctx context.Context, repo string, number int) ([]PRCommit, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	opts := &gh.ListOptions{PerPage: 100}
	var out []PRCommit
	for page := 0; page < maxPRCommitPages; page++ {
		commits, resp, err := c.client.PullRequests.ListCommits(ctx, owner, repoName, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing commits for %s/%s#%d: %w", owner, repoName, number, err)
		}
		for _, rc := range commits {
			if rc == nil {
				continue
			}
			author := safeGetLogin(rc.GetAuthor())
			if author == "" {
				author = rc.GetCommit().GetAuthor().GetName()
			}
			message := rc.GetCommit().GetMessage()
			title := message
			if i := strings.IndexByte(title, '\n'); i >= 0 {
				title = title[:i]
			}
			out = append(out, PRCommit{
				SHA:     rc.GetSHA(),
				Author:  author,
				Title:   strings.TrimSpace(title),
				Message: message,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}
