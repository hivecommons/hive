package github

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	// AuthoredAt is the commit author's timestamp, falling back to the committer
	// timestamp when GitHub omits it.
	AuthoredAt time.Time
	// ParentCount is the number of parents on the commit. A value greater than
	// one identifies merge-from-base maintenance commits that do not, by
	// themselves, address a human review.
	ParentCount int
	// Parents are the parent commit SHAs in git order (first parent first).
	// The hold guard uses them to prove a post-update head is the hive's own
	// merge-from-base rather than a contributor push (hivecommons/hive#14534).
	Parents []string
}

func prCommitTime(rc *gh.RepositoryCommit) time.Time {
	if rc == nil || rc.GetCommit() == nil {
		return time.Time{}
	}
	if a := rc.GetCommit().GetAuthor(); a != nil && a.GetDate().Time != (time.Time{}) {
		return a.GetDate().Time
	}
	if c := rc.GetCommit().GetCommitter(); c != nil {
		return c.GetDate().Time
	}
	return time.Time{}
}

// ListPRCommits returns the commits currently on a PR's branch, oldest first
// (GitHub's list order). Nil-client safe like the other read paths.
func (c *Client) ListPRCommits(ctx context.Context, repo string, number int) ([]PRCommit, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	commits, err := c.listPRRepositoryCommits(ctx, owner, repoName, number)
	if err != nil {
		return nil, err
	}
	out := make([]PRCommit, 0, len(commits))
	for _, rc := range commits {
		author := safeGetLogin(rc.GetAuthor())
		if author == "" {
			author = rc.GetCommit().GetAuthor().GetName()
		}
		message := rc.GetCommit().GetMessage()
		title := message
		if i := strings.IndexByte(title, '\n'); i >= 0 {
			title = title[:i]
		}
		parents := make([]string, 0, len(rc.Parents))
		for _, p := range rc.Parents {
			parents = append(parents, p.GetSHA())
		}
		out = append(out, PRCommit{
			SHA:         rc.GetSHA(),
			Author:      author,
			Title:       strings.TrimSpace(title),
			Message:     message,
			AuthoredAt:  prCommitTime(rc),
			ParentCount: len(rc.Parents),
			Parents:     parents,
		})
	}
	return out, nil
}

// listPRRepositoryCommits is ListPRCommits without the projection: the raw
// commits, oldest first, nils dropped — for callers that need the parents,
// the git identities, or the signature verification.
func (c *Client) listPRRepositoryCommits(ctx context.Context, owner, repoName string, number int) ([]*gh.RepositoryCommit, error) {
	out, _, err := ListPRRepositoryCommits(ctx, c.client, owner, repoName, number)
	return out, err
}

// ListPRRepositoryCommits lists a PR's raw commits, oldest first, nils
// dropped, reading at most maxPRCommitPages pages of 100. complete is false
// when GitHub still offered a further page at that cap, so a caller that must
// see every commit can fail closed instead of judging a truncated list.
func ListPRRepositoryCommits(ctx context.Context, client *gh.Client, owner, repoName string, number int) (commits []*gh.RepositoryCommit, complete bool, err error) {
	opts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < maxPRCommitPages; page++ {
		batch, resp, err := client.PullRequests.ListCommits(ctx, owner, repoName, number, opts)
		if err != nil {
			return nil, false, fmt.Errorf("listing commits for %s/%s#%d: %w", owner, repoName, number, err)
		}
		for _, rc := range batch {
			if rc != nil {
				commits = append(commits, rc)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return commits, true, nil
		}
		opts.Page = resp.NextPage
	}
	return commits, false, nil
}
